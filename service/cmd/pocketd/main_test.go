package main

import (
	"errors"
	"flag"
	"net"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

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

// Without a sandbox that works, pocketd refuses to start, so no command
// ever runs outside it (ADR-0012).
func TestServeRefusesToStartWithoutTheSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pocketd serve runs on Unix hosts only")
	}
	saved := newSandbox
	t.Cleanup(func() { newSandbox = saved })
	for name, build := range map[string]func(string) (*pig.Sandbox, error){
		"no bwrap": func(string) (*pig.Sandbox, error) { return nil, errors.New("bwrap is not installed") },
		"a failing trial": func(string) (*pig.Sandbox, error) {
			return &pig.Sandbox{Bwrap: filepath.Join(t.TempDir(), "missing-bwrap"), Shell: "sh"}, nil
		},
	} {
		newSandbox = build
		// A listener holds the port: serve must fail on the sandbox first,
		// before it tries to bind.
		held, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		err = serve(t.TempDir(), held.Addr().String())
		_ = held.Close()
		if err == nil || strings.Contains(err.Error(), "address already in use") {
			t.Errorf("%s: serve = %v, want the sandbox's error", name, err)
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
