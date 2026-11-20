package terminal

import (
	"bytes"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTheSizeOfAShell(t *testing.T) {
	if rows, cols, err := StartSize(0, 0); err != nil || rows != 24 || cols != 80 {
		t.Fatalf("no size: %d by %d, %v", rows, cols, err)
	}
	for _, ok := range [][2]int{{2, 2}, {500, 1000}, {30, 100}} {
		if err := CheckSize(ok[0], ok[1]); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	for _, no := range [][2]int{{1, 80}, {501, 80}, {24, 1}, {24, 1001}, {0, 80}, {-3, 80}} {
		var b *BadRequest
		if err := CheckSize(no[0], no[1]); !errors.As(err, &b) {
			t.Errorf("%v taken: %v", no, err)
		}
	}
	if _, _, err := StartSize(1, 0); err == nil {
		t.Error("a start of 1 row is taken")
	}
}

func TestTheNameOfAClient(t *testing.T) {
	for _, ok := range []string{"a", "tab-1", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"} {
		if err := CheckTab(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, no := range []string{"", "a b", "a.b", "é", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0"} {
		if err := CheckTab(no); err == nil {
			t.Errorf("%q taken", no)
		}
	}
}

// pi-pocket-team's allowlist: no key, token or pocketd setting reaches a
// shell, and it gets HOME and the terminal's TERM and COLORTERM.
func TestAShellGetsOnlyTheAllowedVariables(t *testing.T) {
	got := Environment([]string{"PATH=/usr/bin", "HOME=/home/team", "LANG=C.UTF-8", "LC_ALL=C", "TZ=UTC", "USER=team",
		"POCKETD_LISTEN=100.1.2.3:8788", "ANTHROPIC_API_KEY=sk-x", "GH_TOKEN=x", "XDG_RUNTIME_DIR=/run/user/1000", "TERM=dumb", "BROKEN"}, "/elsewhere")
	want := []string{"PATH=/usr/bin", "HOME=/home/team", "LANG=C.UTF-8", "LC_ALL=C", "TZ=UTC", "USER=team", "XDG_RUNTIME_DIR=/run/user/1000",
		"TERM=xterm-256color", "COLORTERM=truecolor"}
	if !slices.Equal(got, want) {
		t.Fatalf("environment %q, want %q", got, want)
	}
	if got := Environment([]string{"PATH=/bin"}, "/home/team"); !slices.Contains(got, "HOME=/home/team") {
		t.Fatalf("no HOME: %q", got)
	}
}

func TestTheLoginShellIsTheUsersOwn(t *testing.T) {
	passwd := []byte("root:x:0:0:root:/root:/bin/bash\nteam:x:1000:1000:Team,,,:/home/team:/usr/bin/zsh\nodd:x:1001:1001::/home/odd:sh\n")
	for uid, want := range map[int]string{0: "/bin/bash", 1000: "/usr/bin/zsh", 1001: "/bin/bash", 4242: "/bin/bash"} {
		if got := LoginShell(passwd, uid); got != want {
			t.Errorf("uid %d: %q, want %q", uid, got, want)
		}
	}
}

// fakeTerm is a shell's record with no process, for the rules of its
// output.
func fakeTerm() (*Manager, *term) {
	m := New(Options{Shell: "/bin/sh", Home: "/", Env: []string{}, Now: func() time.Time { return time.Unix(0, 0) }})
	t := &term{changed: make(chan struct{}), tabs: map[string]stamp{}, receipts: map[string]stamp{}}
	t.cond = sync.NewCond(&m.mu)
	return m, t
}

// What is kept is the last Keep bytes, from a character's first byte, and
// a poll from before it gets it from its start; one past the end, the end.
func TestTheOutputKeptAndWhereAPollStarts(t *testing.T) {
	m, tm := fakeTerm()
	m.appendLocked(tm, bytes.Repeat([]byte("a"), Keep-1))
	m.appendLocked(tm, []byte("é🙂"))
	if kept := tm.out.from(0); tm.total != Keep+5 || len(kept) > Keep || !utf8.Valid(kept) {
		t.Fatalf("total %d, kept %d, valid %v", tm.total, len(kept), utf8.Valid(kept))
	}
	start := tm.total - int64(tm.out.size)
	if out, ok := tm.progress(0); !ok || out.From != start || out.To != tm.total || !bytes.HasSuffix(out.Data, []byte("é🙂")) {
		t.Fatalf("a poll from 0: %d to %d, ok %v", out.From, out.To, ok)
	}
	if out, ok := tm.progress(tm.total - 4); !ok || string(out.Data) != "🙂" {
		t.Fatalf("a poll from the last character: %q", out.Data)
	}
	if _, ok := tm.progress(tm.total); ok {
		t.Fatal("a poll at the end has something")
	}
	if _, ok := tm.progress(tm.total + 100); ok || tm.offset(tm.total+100) != tm.total {
		t.Fatal("a poll past the end is not at the end")
	}
	tm.exited = true
	code := 3
	tm.code = &code
	if out, ok := tm.progress(tm.total); !ok || !out.Exited || *out.ExitCode != 3 || len(out.Data) != 0 {
		t.Fatalf("an exited shell's poll: %+v %v", out, ok)
	}
}

// Back pressure follows what the slowest client that still polls drew,
// and with no client, the highest offset asked for.
func TestBackPressureFollowsTheSlowestClient(t *testing.T) {
	m, tm := fakeTerm()
	m.appendLocked(tm, bytes.Repeat([]byte("x"), Keep/2))
	if tm.paused {
		t.Fatal("paused at half the buffer")
	}
	m.appendLocked(tm, []byte("y"))
	if !tm.paused {
		t.Fatal("not paused past half the buffer with nothing asked")
	}
	tm.asked = 1
	m.pressureLocked(tm)
	if tm.paused {
		t.Fatal("still paused once asked")
	}
	now := time.Unix(100, 0)
	tm.tabs["fast"] = stamp{n: tm.total, seen: now}
	tm.tabs["slow"] = stamp{n: 0, seen: now}
	m.pressureLocked(tm)
	if !tm.paused {
		t.Fatal("a client that drew nothing does not hold the shell")
	}
	bounded(tm.tabs, now.Add(TabSpan), TabSpan)
	if len(tm.tabs) != 0 {
		t.Fatalf("clients not seen for the span stay: %v", tm.tabs)
	}
	tm.asked = tm.total
	m.pressureLocked(tm)
	if tm.paused {
		t.Fatal("a client that went still holds the shell")
	}
	for i := range TabsMax + 3 {
		tm.tabs[string(rune('a'+i))] = stamp{seen: now.Add(time.Duration(i) * time.Second)}
	}
	bounded(tm.tabs, now.Add(time.Minute/2), time.Hour)
	if _, ok := tm.tabs["a"]; ok || len(tm.tabs) != TabsMax {
		t.Fatalf("the oldest clients stay: %d", len(tm.tabs))
	}
}

// Codex review of #41, round 4: a write to a full history copies its own
// bytes alone, not what is kept, and what is kept reads back in order,
// from a character's first byte.
func TestTheHistoryCopiesOnlyWhatComes(t *testing.T) {
	var h history
	h.write(bytes.Repeat([]byte("a"), Keep-2))
	h.write([]byte("bé"))
	if h.size != Keep {
		t.Fatalf("kept %d", h.size)
	}
	before := h.copied
	for range 100 {
		h.write([]byte("0123456789"))
	}
	if moved := h.copied - before; moved != 1000 {
		t.Fatalf("1,000 bytes written to a full history moved %d", moved)
	}
	kept := h.from(0)
	if len(kept) != Keep || !bytes.HasSuffix(kept, bytes.Repeat([]byte("0123456789"), 100)) || !utf8.Valid(kept) {
		t.Fatalf("kept %d bytes, ending %q", len(kept), kept[len(kept)-12:])
	}
	if got := h.from(Keep - 3); string(got) != "789" || len(h.from(Keep+5)) != 0 {
		t.Fatalf("the end of what is kept: %q", got)
	}
	// A character cut at the start of what is kept leaves with its bytes.
	var g history
	g.write(bytes.Repeat([]byte("a"), Keep-1))
	g.write([]byte("é"))
	g.write([]byte("x"))
	if first := g.from(0); !utf8.Valid(first) || first[0] == 0xA9 {
		t.Fatalf("what is kept starts within a character: % x", first[:2])
	}
	var big history
	big.write(bytes.Repeat([]byte("z"), 2*Keep))
	if big.size != Keep || len(big.from(0)) != Keep {
		t.Fatalf("a write past Keep kept %d", big.size)
	}
}
