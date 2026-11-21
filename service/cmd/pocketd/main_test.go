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
	"time"

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
		if err := serve(t.TempDir(), "not-an-address", t.TempDir(), 12, 0); !tc.want(err) {
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

// pocketd profiles says why a standing profile whose folder is missing
// cannot start its session, as the API does (Codex review of #43, round
// 4).
func TestListProfilesNamesAMissingStandingFolder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "odrade.md"), []byte("---\nmodel: anthropic/claude-opus-5-5\nhome: /nonexistent/pocketgo-odrade\nsession: standing\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := listProfiles(context.Background(), &out, t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not an existing directory") {
		t.Fatalf("output lacks the missing folder:\n%s", out.String())
	}
}

// pocketd profiles reports the owner's worker list and the model a worker
// runs now, or why none starts (ADR-0022).
func TestListProfilesReportsTheWorkerList(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(data, authFile), []byte(`{"deepseek":{"type":"api_key","key":"sk-test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	list := func() string {
		t.Helper()
		var out strings.Builder
		if err := listProfiles(context.Background(), &out, data, dir); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := list(); !strings.Contains(got, "Workers (the default: no workers.json): deepseek/deepseek-flash") ||
		!strings.Contains(got, "A worker runs: deepseek/deepseek-flash (worker-default)") {
		t.Fatalf("no list:\n%s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, profiles.WorkersFile), []byte(`{"models": ["halogen/qwen", "deepseek/deepseek-v4-pro"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := list(); !strings.Contains(got, "Workers (workers.json): halogen/qwen, deepseek/deepseek-v4-pro") ||
		!strings.Contains(got, "A worker runs: deepseek/deepseek-v4-pro (worker-team)") {
		t.Fatalf("the owner's list:\n%s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, profiles.WorkersFile), []byte(`{"models": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := list(); !strings.Contains(got, "Workers: none start: workers.json: it names 0 models") {
		t.Fatalf("an invalid list:\n%s", got)
	}
}

// pocketd profiles says what the owner's schedules file holds: each entry
// with when it goes out and whose standing session it goes to, or why it
// does not (ADR-0030).
func TestListProfilesReportsTheSchedules(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	for name, text := range map[string]string{
		"boss.md":  "---\nrole: supervisor\nmodel: faux/faux-1\nhome: ~\nsession: standing\n---\n# Boss\n",
		"other.md": "---\nmodel: faux/faux-1\nhome: ~\n---\n# Other\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	list := func() string {
		t.Helper()
		var out strings.Builder
		if err := listProfiles(context.Background(), &out, data, dir); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := list(); !strings.Contains(got, "Schedules: none (schedules.json)") {
		t.Fatalf("no file:\n%s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, profiles.SchedulesFile), []byte(`{"schedules": [
		{"id": "evening", "when": "every mon,thu 19:00", "zone": "Europe/Berlin", "profile": "boss", "message": "gather"},
		{"id": "nudge", "when": "every 2h", "profile": "other", "message": "look"},
		{"id": "fast", "when": "every 5m", "profile": "boss", "message": "look"},
		{"id": "lost", "when": "every day 8:00", "profile": "nobody", "message": "look"}
	]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := list()
	for _, want := range []string{
		"Schedules (schedules.json; clock times with no zone are in ",
		"  evening: mon, thu at 19:00 (Europe/Berlin), to Boss's standing session\n",
		"  nudge: every 2 hours (",
		"): does not go out: profile other keeps no standing session\n",
		"  fast: does not go out: schedule 3's when: a schedule repeats at most every 10 minutes\n",
		"  lost: every day at 08:00 (",
		"): does not go out: no valid profile nobody\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the report lacks %q:\n%s", want, got)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, profiles.SchedulesFile), []byte(`{"schedules": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := list(); !strings.Contains(got, "Schedules: none go out: schedules.json: schedules is no list") {
		t.Fatalf("a broken file:\n%s", got)
	}
}

// The service's zone is TZ's when set, else the one /etc/localtime links
// to, by name, else Go's Local (ADR-0030).
func TestTheServicesZone(t *testing.T) {
	noLink := errors.New("no link")
	for name, tc := range map[string]struct {
		tz   string
		set  bool
		link string
		err  error
		want string
	}{
		"TZ":             {"Europe/Berlin", true, "/usr/share/zoneinfo/Asia/Tokyo", nil, "Europe/Berlin"},
		"TZ with colon":  {":America/Chicago", true, "", noLink, "America/Chicago"},
		"an empty TZ":    {"", true, "", noLink, "UTC"},
		"a TZ unknown":   {"Mars/Olympus", true, "", noLink, ""},
		"the link":       {"", false, "/usr/share/zoneinfo/Asia/Tokyo", nil, "Asia/Tokyo"},
		"a relative one": {"", false, "../usr/share/zoneinfo/America/New_York", nil, "America/New_York"},
		"no link":        {"", false, "", noLink, ""},
		"another link":   {"", false, "/etc/somewhere", nil, ""},
		// A TZ that names a zone's file is that zone, by its name, which a
		// schedule's record can load again (Codex review of #51, round 1).
		"TZ as a file":       {":/usr/share/zoneinfo/Europe/Berlin", true, "", noLink, "Europe/Berlin"},
		"TZ as a bare file":  {"/usr/share/zoneinfo/Asia/Tokyo", true, "", noLink, "Asia/Tokyo"},
		"TZ as another file": {":/etc/localtime", true, "/usr/share/zoneinfo/Asia/Tokyo", nil, ""},
	} {
		got := zoneOf(tc.tz, tc.set)(tc.link, tc.err)
		// The fallback is Go's local zone, whatever this host names it
		// (Codex review of #51, round 1).
		if tc.want == "" && got != time.Local {
			t.Errorf("%s: %s, want Go's local zone", name, got)
		} else if tc.want != "" && got.String() != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

// pocketd models and pocketd profiles list the owner's providers and their
// models, and say why none is registered when the file breaks a rule
// (ADR-0023).
func TestTheCLIListsTheOwnersProviders(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, profiles.ProvidersFile), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"providers": [{"id": "lab", "api": "openai-completions", "baseUrl": "http://lab.example/v1",
		"models": [{"id": "m1", "name": "Lab One", "contextWindow": 32768, "maxTokens": 4096}]}]}`)
	var out strings.Builder
	if err := models(context.Background(), &out, data, dir); err != nil || !strings.Contains(out.String(), "lab/m1\tLab One") {
		t.Fatalf("pocketd models: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := listProfiles(context.Background(), &out, data, dir); err != nil || !strings.Contains(out.String(), "The owner's providers (providers.json): lab") {
		t.Fatalf("pocketd profiles: %v\n%s", err, out.String())
	}
	write(`{"providers": [{"id": "deepseek", "api": "openai-completions", "baseUrl": "http://lab.example/v1",
		"models": [{"id": "m1", "contextWindow": 32768, "maxTokens": 4096}]}]}`)
	out.Reset()
	if err := models(context.Background(), &out, data, dir); err != nil || strings.Contains(out.String(), "m1") ||
		!strings.Contains(out.String(), "none registered: providers.json: provider 1's id is one of pocketd's own") {
		t.Fatalf("pocketd models with a broken file: %v\n%s", err, out.String())
	}
	// Both files are reported, each whatever the other says (Codex review
	// of #34).
	if err := os.WriteFile(filepath.Join(dir, profiles.WorkersFile), []byte(`{"models": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := listProfiles(context.Background(), &out, data, dir); err != nil || !strings.Contains(out.String(), "Workers: none start") ||
		!strings.Contains(out.String(), "The owner's providers: none registered") || !strings.Contains(out.String(), "PiG's catalog: ") {
		t.Fatalf("pocketd profiles with both files broken: %v\n%s", err, out.String())
	}
	// A report cut short after the profiles' table is an error (Codex
	// review of #34, round 2).
	out.Reset()
	if err := listProfiles(context.Background(), &out, data, dir); err != nil {
		t.Fatal(err)
	}
	table := strings.Index(out.String(), "\nWorkers")
	for _, cut := range []int{table + 3, len(out.String()) - 3} {
		if err := listProfiles(context.Background(), &failAfter{n: cut}, data, dir); err == nil {
			t.Fatalf("a report whose writes failed after %d bytes succeeded", cut)
		}
	}
	// A report whose first write fails is an error, whatever later
	// writes do (Codex review of #34, round 3).
	if err := models(context.Background(), &failFirst{}, data, dir); err == nil {
		t.Fatal("pocketd models succeeded with its first row lost")
	}
	// A provider that takes no key is refused at once, while pocketd runs:
	// the data folder's lock is only for a key that is stored (round 3).
	write(`{"providers": [{"id": "lab", "api": "openai-completions", "baseUrl": "http://lab.example/v1",
		"models": [{"id": "m1", "contextWindow": 32768, "maxTokens": 4096}]}]}`)
	unlock, err := lockData(filepath.Join(data, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	err = run([]string{"-data", data, "-profiles", dir, "login", "lab"})
	unlock()
	if err == nil || !strings.Contains(err.Error(), "takes no key") {
		t.Fatalf("a keyless provider's sign-in while pocketd runs: %v", err)
	}
	write(`{"providers": [{"id": "deepseek", "api": "openai-completions", "baseUrl": "http://lab.example/v1",
		"models": [{"id": "m1", "contextWindow": 32768, "maxTokens": 4096}]}]}`)
	// A broken file never stops a built-in provider's sign-in; one of the
	// owner's needs the file.
	if err := run([]string{"-data", data, "-profiles", dir, "login", "lab"}); err == nil || !strings.Contains(err.Error(), "providers.json") {
		t.Fatalf("a sign-in to the owner's provider with a broken file: %v", err)
	}
}

// The desk's cap comes from -desk-cap, else POCKETD_DESK_CAP, else 12;
// one below 1 or not a number stops pocketd (ADR-0014). A flag given wins,
// over an environment value that is not a number too.
func TestDeskCap(t *testing.T) {
	t.Setenv("POCKETD_DESK_CAP", "")
	// Set but empty, as a launch script's empty setting: not a cap.
	if _, err := deskCap("", false); err == nil || !strings.Contains(err.Error(), "POCKETD_DESK_CAP") {
		t.Fatalf("an empty environment cap: %v", err)
	}
	if err := os.Unsetenv("POCKETD_DESK_CAP"); err != nil {
		t.Fatal(err)
	}
	if n, err := deskCap("", false); err != nil || n != 12 {
		t.Fatalf("default %d %v", n, err)
	}
	t.Setenv("POCKETD_DESK_CAP", "16")
	if n, err := deskCap("", false); err != nil || n != 16 {
		t.Fatalf("from the environment %d %v", n, err)
	}
	t.Setenv("POCKETD_DESK_CAP", "lots")
	if n, err := deskCap("8", true); err != nil || n != 8 {
		t.Fatalf("a flag over a bad environment: %d %v", n, err)
	}
	if _, err := deskCap("", false); err == nil || !strings.Contains(err.Error(), "POCKETD_DESK_CAP") {
		t.Fatalf("a cap that is not a number: %v", err)
	}
	// Only serve reads the cap: a bad one stops no other command.
	if err := run([]string{"-data", t.TempDir(), "version"}); err != nil {
		t.Fatalf("version with a bad cap: %v", err)
	}
	if err := run([]string{"-data", t.TempDir(), "serve"}); err == nil || !strings.Contains(err.Error(), "POCKETD_DESK_CAP") {
		t.Fatalf("serve with a bad cap: %v", err)
	}
	if _, err := deskCap("0", true); err == nil || !strings.Contains(err.Error(), "at least 1") {
		t.Fatalf("a cap of 0: %v", err)
	}
	if _, err := deskCap("x", true); err == nil || !strings.Contains(err.Error(), "-desk-cap") {
		t.Fatalf("a flag that is not a number: %v", err)
	}
	// An empty flag, as a launch script's unset variable gives, is not a
	// cap: it never falls back to the default over a smaller one.
	if _, err := deskCap("", true); err == nil || !strings.Contains(err.Error(), "-desk-cap") {
		t.Fatalf("an empty flag: %v", err)
	}
}

// An API key is refused while a session that is no thread's worker runs
// one of its provider's models: the key would run it (ADR-0021; Codex
// review of #29, round 5).
// A key is stored only while no pocketd serves the data folder (Codex
// review of #29, round 6).
func TestAKeyIsNotStoredWhilePocketdServes(t *testing.T) {
	data := t.TempDir()
	unlock, err := lockData(filepath.Join(data, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err = loginKey(context.Background(), data, "deepseek", nil, strings.NewReader("sk-test\n"), &out)
	unlock()
	if err == nil || !strings.Contains(err.Error(), "stop pocketd") {
		t.Fatalf("a key stored while pocketd serves: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, authFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the sign-in file was written: %v", err)
	}
	if err := loginKey(context.Background(), data, "deepseek", nil, strings.NewReader("sk-test\n"), &out); err != nil {
		t.Fatalf("a key stored while nothing serves: %v", err)
	}
}

// failAfter takes n bytes, then fails every write.
type failAfter struct{ n int }

func (f *failAfter) Write(p []byte) (int, error) {
	if len(p) > f.n {
		written := f.n
		f.n = 0
		return written, errors.New("the disk is full")
	}
	f.n -= len(p)
	return len(p), nil
}

// failFirst fails its first write and takes every later one.
type failFirst struct{ failed bool }

func (f *failFirst) Write(p []byte) (int, error) {
	if !f.failed {
		f.failed = true
		return 0, errors.New("the disk is full")
	}
	return len(p), nil
}
