package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/pig"
)

func TestParseInterleavedAcceptsFlagsAnywhere(t *testing.T) {
	for _, args := range [][]string{
		{"add", "laptop", "-data", "/d"},
		{"-data", "/d", "add", "laptop"},
		{"add", "-data", "/d", "laptop"},
	} {
		flags := flag.NewFlagSet("t", flag.ContinueOnError)
		data := flags.String("data", "default", "")
		rest, err := parseInterleaved(flags, args)
		if err != nil || *data != "/d" || !reflect.DeepEqual(rest, []string{"add", "laptop"}) {
			t.Errorf("%v: data %q rest %v err %v", args, *data, rest, err)
		}
	}
}

func TestParseCommand(t *testing.T) {
	for _, c := range []struct {
		args    []string
		command string
		rest    []string
		err     bool
	}{
		{nil, "serve", nil, false},
		{[]string{"-data", "/d"}, "serve", nil, false},
		{[]string{"-data", "/d", "device", "add", "laptop"}, "device", []string{"add", "laptop"}, false},
		{[]string{"device", "add", "laptop", "-data", "/d"}, "device", []string{"add", "laptop"}, false},
		{[]string{"login", "anthropic"}, "login", []string{"anthropic"}, false},
		{[]string{"serve", "extra"}, "", nil, true},
		{[]string{"login"}, "", nil, true},
		{[]string{"frobnicate"}, "", nil, true},
	} {
		flags := flag.NewFlagSet("t", flag.ContinueOnError)
		flags.String("data", "", "")
		command, rest, err := parseCommand(flags, c.args)
		if (err != nil) != c.err || command != c.command || !reflect.DeepEqual(rest, c.rest) {
			t.Errorf("%v: %q %v %v", c.args, command, rest, err)
		}
	}
}

func TestLockDataIsExclusive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pocketd serve runs on Unix hosts only")
	}
	path := filepath.Join(t.TempDir(), lockFile)
	unlock, err := lockData(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockData(path); err == nil {
		t.Fatal("a second lock of the data folder succeeded")
	}
	unlock()
	again, err := lockData(path)
	if err != nil {
		t.Fatalf("the lock after unlock: %v", err)
	}
	again()
}

// Without a sandbox that works, pocketd refuses to start, before it binds,
// so no command ever runs outside it (ADR-0012).
func TestServeRefusesToStartWithoutTheSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pocketd serve runs on Unix hosts only")
	}
	saved := newSandbox
	t.Cleanup(func() { newSandbox = saved })
	errNoBwrap := errors.New("bwrap is not installed")
	for name, tc := range map[string]struct {
		build func(string, string) (*pig.Sandbox, error)
		want  func(error) bool
	}{
		"no bwrap": {
			build: func(string, string) (*pig.Sandbox, error) { return nil, errNoBwrap },
			want:  func(err error) bool { return errors.Is(err, errNoBwrap) },
		},
		"a failing check": {
			build: func(string, string) (*pig.Sandbox, error) {
				return &pig.Sandbox{Bwrap: filepath.Join(t.TempDir(), "missing-bwrap"), Shell: "sh"}, nil
			},
			want: func(err error) bool { return err != nil && strings.Contains(err.Error(), "sandbox check failed") },
		},
	} {
		newSandbox = tc.build
		// An address that cannot be bound: reaching the bind would fail
		// with a different error, so a pass shows the sandbox came first.
		if err := serve(t.TempDir(), "not-an-address", t.TempDir(), 12); !tc.want(err) {
			t.Errorf("%s: serve = %v", name, err)
		}
	}
}

func TestSdNotifyWritesToTheSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("systemd runs on Linux")
	}
	path := filepath.Join(t.TempDir(), "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	t.Setenv("NOTIFY_SOCKET", path)
	if err := sdNotify("READY=1"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, _, err := listener.ReadFromUnix(buf)
	if err != nil || string(buf[:n]) != "READY=1" {
		t.Fatalf("received %q, %v", buf[:n], err)
	}
	t.Setenv("NOTIFY_SOCKET", "")
	if err := sdNotify("READY=1"); err != nil {
		t.Fatalf("without a socket: %v", err)
	}
}

// An empty -profiles turns profiles off: the server gets no source, so it
// never reads profiles from the working directory.
func TestAnEmptyProfileFolderTurnsProfilesOff(t *testing.T) {
	if source, err := profileSource("", "/home/team"); source != nil || err != nil {
		t.Fatalf("an empty folder gave the source %#v, %v", source, err)
	}
	dir := t.TempDir()
	if source, err := profileSource(dir, "/home/team"); err != nil {
		t.Fatal(err)
	} else if folder, ok := source.(profiles.Folder); !ok || folder.Dir != dir || folder.Verified == nil {
		t.Fatalf("a folder gave %#v", source)
	}
	var out strings.Builder
	if err := listProfiles(context.Background(), &out, t.TempDir(), ""); err != nil || !strings.Contains(out.String(), "Profiles are off") {
		t.Fatalf("pocketd profiles with an empty folder: %q, %v", out.String(), err)
	}
}

// pocketd profiles names a tool pocketd does not have, as the API does,
// before any question of models.
func TestListProfilesNamesAnUnknownTool(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vera.md"), []byte("---\nmodel: anthropic/claude-opus-5-5\ntools: [read, teleport]\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := listProfiles(context.Background(), &out, t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "teleport") {
		t.Fatalf("output lacks the unknown tool:\n%s", out.String())
	}
}

// The desk's cap comes from -desk-cap, else POCKETD_DESK_CAP, else 12;
// one below 1 or not a number stops pocketd (ADR-0014).
func TestDeskCap(t *testing.T) {
	t.Setenv("POCKETD_DESK_CAP", "")
	if n, err := defaultDeskCap(); err != nil || n != 12 {
		t.Fatalf("default %d %v", n, err)
	}
	t.Setenv("POCKETD_DESK_CAP", "16")
	if n, err := defaultDeskCap(); err != nil || n != 16 {
		t.Fatalf("from the environment %d %v", n, err)
	}
	t.Setenv("POCKETD_DESK_CAP", "lots")
	if err := run([]string{"version"}); err == nil || !strings.Contains(err.Error(), "POCKETD_DESK_CAP") {
		t.Fatalf("a cap that is not a number: %v", err)
	}
	t.Setenv("POCKETD_DESK_CAP", "")
	if err := run([]string{"-desk-cap", "0", "version"}); err == nil || !strings.Contains(err.Error(), "at least 1") {
		t.Fatalf("a cap of 0: %v", err)
	}
}
