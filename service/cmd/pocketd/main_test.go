package main

import (
	"flag"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
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
