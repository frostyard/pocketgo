package pig

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sandbox runs each command a tool starts under bubblewrap (ADR-0012). The
// Hide folders are empty, the ReadOnly paths and the Shown folders cannot
// change, the Pin folders cannot be renamed or removed, and the command
// sees only its own processes and dies with pocketd. Everything else of the
// host stays as it is: agents keep their tools, repositories, caches and
// network.
type Sandbox struct {
	// Bwrap is a root-owned bwrap program; Shell runs a command given as a
	// string, as PiG's local environment runs it.
	Bwrap, Shell string
	// Pin are the folders above a protected path that the service user
	// could rename, shallowest first. Each is bound onto itself: a mount
	// point cannot be renamed or removed, through any mount of it, so the
	// paths below stay where Hide and ReadOnly cover them, from one
	// command to the next.
	Pin []string
	// ReadOnly are what decides how pocketd starts, under every path a
	// mount exposes them at: its program, its environment file and the
	// user's systemd configuration.
	ReadOnly []string
	// Shown are the owner's folders that agents read but never change,
	// under every path a mount exposes them at: the skills folder a gather
	// reads (ADR-0033) and the uploads folder (ADR-0029), read-only since
	// ADR-0035. A command finds them read-only; the file tools read them
	// and refuse to change them. pocketd itself, outside the sandbox,
	// writes them as before.
	Shown []string
	// Hide are folders replaced by an empty tmpfs, under every path a mount
	// exposes them at: the data folder, which holds the sign-ins and
	// transcripts, and /run/user, which holds every runtime folder with its
	// D-Bus and systemd sockets, so one a login creates while a command
	// runs stays hidden from it too.
	Hide []string

	// kept is what keepShown last found at each Shown path.
	kept *keptFolders
	// roots are the Shown folders as pocketd named them, resolved; the
	// other Shown paths are where mounts show them too, which may be a
	// single file of one. nil takes every Shown path for a root, as a
	// test's literal does.
	roots []string
	// mounts reads the host's mount table now, for keepShown to find
	// where a mount shows a replaced folder; nil reads none, as a test's
	// sandbox with no table does.
	mounts func() ([]mount, error)
	// mark is a random word every bwrap this sandbox starts carries
	// (sandboxMark), so a stop kills those alone, never a child of pocketd
	// the owner started as bwrap from a terminal (Codex review of #63,
	// round 5); "" for a test's literal, whose stop kills nothing.
	mark string
	// self is pocketd's program, which runs each command through its
	// GateHelper in the sandbox; "" runs no gate, as a test's literal.
	self string
}

// sandboxMark is the variable bwrap sets in each command it runs, to the
// sandbox's mark: harmless to the command, and the word its stop tells
// pocketd's own sandboxes by.
const sandboxMark = "POCKETD_SANDBOX"

// randomMark is a new sandbox's mark: 16 random hexadecimal characters.
func randomMark() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// root says whether a Shown path is a folder pocketd named, not where a
// mount shows one.
func (s *Sandbox) root(path string) bool {
	return s.roots == nil || slices.Contains(s.roots, path)
}

// watched are the paths keepShown keeps: the Shown paths, and the pinned
// folders above them. A pinned folder replaced outside, the shown folder
// moved into the new one, takes the running commands' binds with the old
// one as a replaced shown folder does, though the shown folder is the same
// (Codex review of #63, round 5).
func (s *Sandbox) watched() []string {
	out := slices.Clone(s.Shown)
	for _, pin := range s.Pin {
		for _, dir := range s.Shown {
			if pin != dir && within(dir, pin) && !slices.Contains(out, pin) {
				out = append(out, pin)
			}
		}
	}
	return out
}

// keptFolders are the Shown folders as keepShown last kept them, by path,
// each held open: while it is held, its inode cannot be used again, so a
// folder made at its path after it went is always another one (Codex
// review of #63, round 2).
type keptFolders struct {
	// turn is held by the one look at a time: a token in a channel of
	// one, so a look that waits for its turn leaves when its context ends
	// (Codex review of #63, round 9).
	turn chan struct{}
	held map[string]*os.File
	// unfinished is a stop that failed: every look stops again until one
	// succeeds, whatever it finds then (Codex review of #63, round 6).
	unfinished bool
}

// keptInit makes a Sandbox's keptFolders once, for one a test writes as a
// literal.
var keptInit sync.Mutex

func (s *Sandbox) keptFolders() *keptFolders {
	keptInit.Lock()
	defer keptInit.Unlock()
	if s.kept == nil {
		s.kept = &keptFolders{turn: make(chan struct{}, 1), held: map[string]*os.File{}}
	}
	return s.kept
}

// take waits for the look's turn until ctx ends; release ends it. The look
// that has its turn finishes what it started, a stop included, whatever
// its own context: only the waiting is given up.
func (k *keptFolders) take(ctx context.Context) error {
	select {
	case k.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("commands run in a sandbox (ADR-0012) that keeps its shown folders read-only (ADR-0035), and the look at them did not come in time: %w", ctx.Err())
	}
}

func (k *keptFolders) release() { <-k.turn }

// hold keeps what is at path from now on, held open, and lets what was
// kept before go: a folder, or, where root is false, a single file a mount
// shows (Codex review of #63, round 3). It opens without following a link
// and without waiting on a named pipe, so nothing at path holds the
// watch.
func (k *keptFolders) hold(path string, root bool) error {
	f, err := openHeld(path)
	if err != nil {
		return err
	}
	opened, err := f.Stat()
	if err == nil {
		var named fs.FileInfo
		named, err = os.Lstat(path)
		switch {
		case err != nil:
		case !os.SameFile(opened, named):
			err = fmt.Errorf("%s changed as it was opened", path)
		case root && !opened.IsDir(), !opened.IsDir() && !opened.Mode().IsRegular():
			err = fmt.Errorf("%s is no folder", path)
		}
	}
	if err != nil {
		_ = f.Close()
		return err
	}
	if old := k.held[path]; old != nil {
		_ = old.Close()
	}
	k.held[path] = f
	return nil
}

// stopRunning stops every command running in the sandbox
// (stopSandboxes): each bwrap started by its path, carrying its mark;
// tests make it fail.
var stopRunning = stopSandboxes

// Command is the bwrap command line that runs command in the sandbox: a
// string through the shell, an argv as it is. It never returns a command
// that runs outside the sandbox. It keeps the Shown folders first
// (keepShown): one missing now, as one the owner removed by hand is, is
// made again, empty.
func (s *Sandbox) Command(command any) ([]string, error) {
	return s.CommandContext(context.Background(), command)
}

// CommandContext is Command, waiting for the look at the Shown folders
// only until ctx ends (keptFolders.take).
func (s *Sandbox) CommandContext(ctx context.Context, command any) ([]string, error) {
	places, err := s.keep(ctx)
	if err != nil {
		return nil, err
	}
	args := []string{s.Bwrap, "--dev-bind", "/", "/", "--unshare-pid", "--proc", "/proc", "--die-with-parent"}
	if s.mark != "" {
		args = append(args, "--setenv", sandboxMark, s.mark)
	}
	// Pins first: a bind of a folder shows the host's view of everything
	// below it, so the mounts below must come after.
	for _, dir := range s.Pin {
		args = append(args, "--bind", dir, dir)
	}
	for _, path := range s.ReadOnly {
		args = append(args, "--ro-bind", path, path)
	}
	// After the read-only paths, which a Shown folder that holds one keeps
	// read-only, and before the masks, which hide a Shown folder inside a
	// hidden one.
	for _, dir := range s.Shown {
		args = append(args, "--ro-bind", dir, dir)
	}
	for _, dir := range s.Hide {
		args = append(args, "--tmpfs", dir)
	}
	gate, bind := s.gate()
	args = append(append(args, bind...), "--")
	// The command starts through the gate, which checks inside the
	// sandbox, as it starts, that every path a mount shows a shown folder
	// at is read-only there (GateHelper): a command admitted here and
	// started after a mount showed one somewhere new refuses to run. A
	// folder a mask hides is checked by its place, where its files are,
	// since its own path shows the mask (Codex review of #63, round 8).
	var gated []string
	for _, root := range s.roots {
		if !s.Hides(root) {
			gated = append(gated, root)
		}
	}
	if s.self != "" && (len(gated) > 0 || len(places) > 0) {
		args = append(append(args, gate, GateHelper), gated...)
		if len(places) > 0 {
			args = append(append(args, gatePlaced), places...)
		}
		var masks []string
		for _, dir := range s.Hide {
			for _, root := range gated {
				if dir != root && within(dir, root) && !slices.Contains(masks, dir) {
					masks = append(masks, dir)
				}
			}
		}
		if len(masks) > 0 {
			args = append(append(args, gateMasked), masks...)
		}
		args = append(args, "--")
	}
	switch c := command.(type) {
	case string:
		return append(args, s.Shell, "-c", c), nil
	case []string:
		if len(c) == 0 {
			return nil, errors.New("empty argv: no program to run")
		}
		// pocketd's own program, as a gather's read helper runs it, runs
		// where the sandbox shows it, as the gate does (Codex review of
		// #63, round 9).
		if s.self != "" && c[0] == s.self && gate != s.self {
			return append(append(args, gate), c[1:]...), nil
		}
		return append(args, c...), nil
	}
	return nil, fmt.Errorf("a command is a string or a []string, not %T", command)
}

// gateName is the file pocketd's program is bound read-only at, in a
// hidden folder's mask, when a mask hides the program itself.
const gateName = ".pocketd-gate"

// gate is the path the sandbox runs pocketd's program at, for its
// GateHelper, and the bwrap arguments that put it there, after the masks.
// That is the program's own path, unless a mask hides it, as one in the
// data folder is: then its mask shows the program, read-only, at gateName
// and nothing else (Codex review of #63, round 8). The mask is the first
// that no other covers, so no later mask hides the program again.
func (s *Sandbox) gate() (program string, bind []string) {
	if s.self == "" || !s.Hides(s.self) {
		return s.self, nil
	}
	for _, dir := range s.Hide {
		covered := false
		for _, other := range s.Hide {
			covered = covered || other != dir && within(dir, other)
		}
		if !covered {
			program = filepath.Join(dir, gateName)
			return program, []string{"--ro-bind", s.self, program}
		}
	}
	return s.self, nil
}

// keepShown keeps each Shown folder bound read-only (ADR-0035), before
// each command and at each tick of watchShown. Only pocketd and the owner,
// outside the sandbox, can remove or replace one: in the sandbox each is a
// mount point, and the folders above it that the user could rename are
// pinned. But a folder removed or renamed outside takes the read-only
// binds of the commands running then with it (the kernel detaches a mount
// whose point goes), and a new folder at its path is writable to them.
// So, when a Shown folder is missing, is another folder than the one kept,
// or has a symbolic link on the way to it, keepShown first stops every
// command running in the sandbox (stopRunning), then:
//   - makes a missing folder again, empty, as newSandbox made it: bwrap
//     cannot bind a folder that is not there, and binding it only when it
//     is there would let a command make it;
//   - keeps the other folder from now on;
//   - refuses this command and every next one while a link stands on the
//     way, which would lead the bind where the file tools do not guard
//     (guardedEnv.readOnly): a restart, whose start refuses a link the
//     user can change, ends that.
//
// What a command running then put in a new folder before it was stopped,
// at most a tick of watchShown after the change, stays.
//
// Nothing is kept anew until the stop succeeded: a stop that failed is
// tried again at the next look, which finds the change again (Codex review
// of #63, round 2).
func (s *Sandbox) keepShown(ctx context.Context) error {
	_, err := s.keep(ctx)
	return err
}

// keep is keepShown's work. It gives the places of the shown folders in
// the mount table it read, for the gate (placesOf), or none when it read
// no table. It waits for its turn only until ctx ends.
func (s *Sandbox) keep(ctx context.Context) ([]string, error) {
	if len(s.Shown) == 0 {
		return nil, nil
	}
	kept := s.keptFolders()
	if err := kept.take(ctx); err != nil {
		return nil, err
	}
	defer kept.release()
	var linked, missing, fresh []string
	stop := false
	for _, dir := range s.watched() {
		if real, err := resolvePath(dir); err != nil || real != dir {
			linked = append(linked, dir)
			stop = true
			continue
		}
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, dir)
			stop = true
		case err != nil:
			return nil, fmt.Errorf("commands run in a sandbox (ADR-0012) that keeps %s read-only (ADR-0035), which cannot be looked at: %w", dir, err)
		default:
			held := kept.held[dir]
			if held == nil {
				// The first look keeps the folder as it is: no command ran
				// with another.
				fresh = append(fresh, dir)
				continue
			}
			if was, err := held.Stat(); err != nil || !os.SameFile(was, info) {
				fresh = append(fresh, dir)
				stop = true
			}
		}
	}
	// Every look reads the mount table again. A mount that shows a shown
	// folder, or a part of one, where this sandbox does not keep it
	// read-only, as a replaced folder a mount showed elsewhere, or a folder
	// a mount shows that the owner moved into a shown one, stops every
	// command, and every change of the file tools, until that mount goes or
	// a restart binds it too (Codex review of #63, rounds 3 and 4).
	var places []string
	var unkept, unread error
	if s.mounts != nil {
		places, unkept, unread = s.unkeptAlias()
		stop = stop || unkept != nil
	}
	// A stop a change asks for happens before any failure is reported: a
	// table that cannot be read refuses new work, but leaves no command
	// running with a bind that went (Codex review of #63, round 5). One
	// that failed is done again at every look until it succeeds, though
	// what asked for it is gone (round 6).
	if stop || kept.unfinished {
		if err := stopRunning(s.Bwrap, s.mark); err != nil {
			kept.unfinished = true
			return nil, err
		}
		kept.unfinished = false
	}
	if unread != nil {
		return nil, unread
	}
	if len(linked) > 0 {
		return nil, fmt.Errorf("commands run in a sandbox (ADR-0012) that keeps %s read-only (ADR-0035), and a symbolic link has stood on the way to it since pocketd started: restart pocketd", strings.Join(linked, ", "))
	}
	if unkept != nil {
		return nil, unkept
	}
	// The folders pocketd named first: one made again shows again where
	// a mount showed it, so only a path still missing then lost its mount
	// (Codex review of #63, round 4).
	for _, dir := range missing {
		if !s.root(dir) {
			continue
		}
		if err := makeShown(dir); err != nil {
			return nil, fmt.Errorf("commands run in a sandbox (ADR-0012) that keeps %s read-only (ADR-0035), and it is missing and cannot be made again: %w", dir, err)
		}
	}
	for _, dir := range missing {
		if _, err := os.Lstat(dir); !s.root(dir) && err != nil {
			return nil, fmt.Errorf("commands run in a sandbox (ADR-0012) that keeps %s read-only (ADR-0035), and the mount that showed a shown folder there is gone: restart pocketd", dir)
		}
	}
	changed := slices.Concat(fresh, missing)
	for _, dir := range changed {
		if err := kept.hold(dir, s.root(dir) || !slices.Contains(s.Shown, dir)); err != nil {
			return nil, fmt.Errorf("commands run in a sandbox (ADR-0012) that keeps %s read-only (ADR-0035), which cannot be held: %w", dir, err)
		}
	}
	return places, nil
}

// makeShown makes a missing shown folder again, empty; tests make it show
// a mount's path again too, as the kernel does for a bind mount of the
// folder above.
var makeShown = func(dir string) error { return os.MkdirAll(dir, 0o700) }

// errNoMount is a mount table that shows no mount a shown folder is on,
// as one read in a chroot that is no mount point of its own can: its
// aliases cannot be told, so the folder cannot be kept read-only.
var errNoMount = errors.New("the mount table shows no mount it is on")

// unkeptAlias is a problem when the mount table shows a shown folder, or a
// part of one, somewhere this sandbox does not keep read-only now, or
// shows no mount one is on; err is a table that cannot be read. places
// are where the shown folders' files are in that table (placesOf).
func (s *Sandbox) unkeptAlias() (places []string, problem, err error) {
	mounts, err := s.mounts()
	if err != nil {
		return nil, nil, fmt.Errorf("commands run in a sandbox (ADR-0012) that keeps its shown folders read-only (ADR-0035), and the mount table cannot be read: %w", err)
	}
	roots := s.roots
	if roots == nil {
		roots = s.Shown
	}
	for _, root := range roots {
		if _, ok := mountAt(mounts, root); !ok {
			return nil, fmt.Errorf("commands run in a sandbox (ADR-0012) that keeps %s read-only (ADR-0035), and %w: restart pocketd", root, errNoMount), nil
		}
	}
	// All the folders at once, as newSandbox found them: a path inside
	// another shown folder is kept by that folder's bind (Codex review of
	// #63, round 5). A path a mask hides is no alias, as newSandbox left
	// it out (unmasked).
	now := s.unmasked(withAliases(roots, mounts))
	for _, alias := range now {
		if !slices.Contains(s.Shown, alias) {
			return nil, fmt.Errorf("commands run in a sandbox (ADR-0012) that keeps its shown folders read-only (ADR-0035), and a mount shows one, or a part of one, at %s too, which this sandbox does not keep read-only: restart pocketd", alias), nil
		}
	}
	// A path a mount showed a folder at, which no mount shows it at now,
	// holds something else, as the empty folder an unmount leaves: kept
	// as an alias, it would be bound read-only and watched for nothing
	// (Codex review of #63, round 7).
	for _, alias := range s.Shown {
		if !slices.Contains(now, alias) {
			return nil, fmt.Errorf("commands run in a sandbox (ADR-0012) that keeps its shown folders read-only (ADR-0035), and the mount that showed one at %s is gone: restart pocketd", alias), nil
		}
	}
	return placesOf(roots, mounts), nil, nil
}

// unmasked is paths without the aliases a mask hides: a command finds the
// mask's empty tmpfs there, and the file tools refuse the path (Guarded),
// so no bind keeps it, no look watches it, and no mount that shows a
// shown folder there, or stops showing one, stops commands (Codex review
// of #63, round 8). A folder pocketd named stays: pocketd reads and
// writes it, and makes it again.
func (s *Sandbox) unmasked(paths []string) []string {
	var out []string
	for _, path := range paths {
		if s.root(path) || !s.Hides(path) {
			out = append(out, path)
		}
	}
	return out
}

// placesOf are where the files of the folders roots are, in the mount
// table mounts: for each root, the device of the mount it is on and the
// path inside that mount's filesystem, then the same for each mount seen
// inside it, as "0:45/home/u/skills". The gate finds every path a mount
// shows a place at in the sandbox's own table, the devices being the
// host's, though a mask hides the root's own path (Codex review of #63,
// round 8).
func placesOf(roots []string, mounts []mount) []string {
	var out []string
	add := func(device, inFS string) {
		if place := device + inFS; !slices.Contains(out, place) {
			out = append(out, place)
		}
	}
	for _, root := range roots {
		m, ok := mountAt(mounts, root)
		if !ok {
			continue
		}
		rel, err := filepath.Rel(m.Point, root)
		if err != nil {
			continue
		}
		add(m.Device, filepath.Join(m.Root, rel))
		for _, c := range mounts {
			if c.Point == root || !within(c.Point, root) {
				continue
			}
			if at, ok := mountAt(mounts, c.Point); ok && at.ID == c.ID {
				add(c.Device, c.Root)
			}
		}
	}
	return out
}

// watchShown runs keepShown at each tick until ctx ends, so a Shown
// folder replaced while commands run stops them though no new command
// starts. A failure waits for the next tick, or the next command, which
// reports it.
func (s *Sandbox) watchShown(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			_ = s.keepShown(ctx)
		}
	}
}

// shownEvery is how often an Engine has its sandbox look at the Shown
// folders (watchShown).
const shownEvery = 250 * time.Millisecond

// Hides says whether a command in the sandbox finds path hidden, inside a
// folder Hide empties, as it is named or as its links resolve now, those
// of the folders above a part that does not exist yet included (Codex
// review of #49, round 1). The uploads folder must not be (ADR-0029).
func (s *Sandbox) Hides(path string) bool {
	paths := []string{filepath.Clean(path)}
	if resolved, err := resolvePath(path); err == nil {
		paths = append(paths, resolved)
	}
	for _, p := range paths {
		for _, dir := range s.Hide {
			if within(p, dir) {
				return true
			}
		}
	}
	return false
}

// Guarded are the paths the file tools refuse as well, so a write or an
// edit cannot do what the sandbox stops a command from doing.
func (s *Sandbox) Guarded() []string {
	return slices.Concat(s.Hide, s.ReadOnly)
}

// sandboxPaths are the host paths a sandbox protects.
type sandboxPaths struct {
	Bwrap, Shell string
	// Data is pocketd's data folder; Home the service user's home; Program
	// pocketd's running executable, and Launch the name its unit starts it
	// by (deploy/systemd/pocketd.service: ~/.local/bin/pocketd); Profiles
	// the agents' profile folder, empty for none.
	Data, Home, Program, Launch, Profiles string
	// Self is the program that runs each command through its GateHelper
	// in the sandbox: pocketd, as Program is; "" runs no gate.
	Self string
	// Shown are the owner's folders agents read but never change: the
	// skills folder and the uploads folder (ADR-0035). An empty one is
	// none. newSandbox rewrites each to the path it keeps read-only, links
	// resolved, so pocketd reads and writes exactly the folder kept, never
	// one a link retargeted since leads to (Codex review of #63, round 5).
	Shown []*string
	// Runtime are the folders that hold runtime folders: /run/user, and an
	// XDG_RUNTIME_DIR outside it.
	Runtime []string
	// ConfigHomes and DataHomes are the XDG configuration and data folders
	// the user's systemd manager reads: the defaults, and the ones the
	// environment names.
	ConfigHomes, DataHomes []string
	// Mounts are the host's mounts (/proc/self/mountinfo), and ReadMounts
	// reads them again, as keepShown does for a folder replaced while
	// pocketd runs; nil reads none.
	Mounts     []mount
	ReadMounts func() ([]mount, error)
	// Writable reports whether the service user may change a folder's
	// entries.
	Writable func(dir string) bool
}

// readMountinfo is the host's mount table, as this process sees it.
func readMountinfo() ([]mount, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("commands run under bubblewrap (ADR-0012), which needs the host's mounts: %w", err)
	}
	defer func() { _ = f.Close() }()
	return parseMountinfo(f)
}

// NewSandbox finds a root-owned bwrap and fills the paths ADR-0012 names for
// the data folder data, and the agents' profile folder profiles (ADR-0013),
// read-only like pocketd's start files: a profile decides what an agent is
// told and may do. The shown folders, the skills folder and the uploads
// folder, are read-only to agents too, but readable (ADR-0035); an empty
// one is none. Each is rewritten to the path the sandbox keeps read-only,
// which pocketd then uses (sandboxPaths.Shown).
func NewSandbox(data, profiles string, shown ...*string) (*Sandbox, error) {
	bwrap, err := trustedBwrap()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	program, err := os.Executable()
	if err != nil {
		return nil, err
	}
	mounts, err := readMountinfo()
	if err != nil {
		return nil, err
	}
	p := sandboxPaths{Bwrap: bwrap, Shell: shell(), Data: data, Home: home, Program: program, Profiles: profiles, Shown: shown,
		Launch: filepath.Join(home, ".local", "bin", "pocketd"), Runtime: []string{"/run/user"},
		ConfigHomes: []string{filepath.Join(home, ".config")}, DataHomes: []string{filepath.Join(home, ".local", "share")},
		Mounts: mounts, ReadMounts: readMountinfo, Writable: writable, Self: program}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(dir) && !within(dir, "/run/user") {
		p.Runtime = append(p.Runtime, dir)
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		p.ConfigHomes = append(p.ConfigHomes, dir)
	}
	if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		p.DataHomes = append(p.DataHomes, dir)
	}
	return newSandbox(p)
}

// newSandbox is the sandbox of p. It creates the configuration paths and
// the shown folders that do not exist yet, empty, so each can be bound
// read-only: a command could otherwise create one that pocketd's next
// start, or a gather, reads.
func newSandbox(p sandboxPaths) (*Sandbox, error) {
	s := &Sandbox{Bwrap: p.Bwrap, Shell: p.Shell, mark: randomMark(), self: p.Self}
	data, err := filepath.Abs(p.Data)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, home := range p.ConfigHomes {
		dirs = append(dirs, filepath.Join(home, "systemd"), filepath.Join(home, "environment.d"))
	}
	for _, home := range p.DataHomes {
		dirs = append(dirs, filepath.Join(home, "systemd"))
	}
	// The profile folder is read-only like the systemd folders, but systemd
	// does not follow links in it: pocketd reads a profile only as a
	// regular file there (ADR-0013), so its contents are not scanned.
	var profileDir []string
	if p.Profiles != "" {
		profiles, err := filepath.Abs(p.Profiles)
		if err != nil {
			return nil, err
		}
		profileDir = []string{profiles}
	}
	// The shown folders are read-only like the profile folder, but agents
	// read them: the skills a gather reads, and the owner's uploads
	// (ADR-0035). What lies inside one is not scanned: pocketd reads a
	// skill as the sandbox shows it, and an upload never through a link.
	var shown []string
	for _, named := range p.Shown {
		if named == nil || *named == "" {
			continue
		}
		dir, err := filepath.Abs(*named)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(shown, dir) {
			shown = append(shown, dir)
		}
	}
	envFile := filepath.Join(p.Home, ".config", "pocketd.env")
	var launch []string
	if p.Launch != "" {
		if _, err := os.Lstat(p.Launch); err == nil {
			launch = []string{p.Launch}
		}
	}
	names := slices.Concat([]string{data, envFile}, dirs, profileDir, shown, launch)
	// systemd and pocketd open these by name: a link on the way that the
	// user could change would lead them past what the sandbox protects.
	for _, path := range names {
		if err := noUserLinks(path, p.Writable); err != nil {
			return nil, err
		}
	}
	for _, dir := range slices.Concat(dirs, profileDir) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	// A shown folder named through a link only root can change is made
	// where the link leads, as making it through the link would.
	for _, dir := range shown {
		real, err := resolvePath(dir)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(real, 0o700); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(envFile, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()

	resolved, err := filepath.EvalSymlinks(data)
	if err != nil {
		return nil, err
	}
	hide := []string{resolved}
	for _, dir := range p.Runtime {
		// A runtime folder that is missing now could appear under a
		// parent the mask does not cover: refuse rather than guess.
		if err := noUserLinks(dir, p.Writable); err != nil {
			return nil, err
		}
		dir, err := absolute(dir)
		if err != nil {
			return nil, fmt.Errorf("commands run in a sandbox (ADR-0012), and the runtime folder cannot be hidden: %w", err)
		}
		if !slices.Contains(hide, dir) {
			hide = append(hide, dir)
		}
	}
	s.Hide = withAliases(hide, p.Mounts)
	// A tmpfs covers a folder only: a hidden path that is a file, as a
	// bind mount of one file can be, cannot be masked.
	for _, path := range s.Hide {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return nil, fmt.Errorf("commands run in a sandbox (ADR-0012), and %s, which shows a protected path, is not a folder pocketd can hide", path)
		}
	}
	resolve := func(paths []string) ([]string, error) {
		var out []string
		for _, path := range paths {
			resolved, err := absolute(path)
			if err != nil {
				return nil, err
			}
			if !slices.Contains(out, resolved) {
				out = append(out, resolved)
			}
		}
		return out, nil
	}
	// A link in the systemd folders is read where it points: it must stay
	// inside pocketd's start files, or point to a file only root can
	// change. The profile folder is read-only too, but not a target
	// systemd may be led to: pocketd checks a profile's names only when it
	// reads it as a profile.
	startFiles, err := resolve(slices.Concat([]string{p.Program, envFile}, dirs, launch))
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		if err := linksStayProtected(dir, startFiles); err != nil {
			return nil, err
		}
	}
	readOnly, err := resolve(slices.Concat([]string{p.Program, envFile}, dirs, profileDir, launch))
	if err != nil {
		return nil, err
	}
	s.ReadOnly = withAliases(readOnly, p.Mounts)
	// Each shown folder resolved once: the path kept is the path pocketd
	// is told to use (sandboxPaths.Shown).
	var resolvedShown []string
	kept := map[string]string{}
	for _, dir := range shown {
		real, err := absolute(dir)
		if err != nil {
			return nil, err
		}
		kept[dir] = real
		if !slices.Contains(resolvedShown, real) {
			resolvedShown = append(resolvedShown, real)
		}
	}
	// A mount table that shows no mount a protected path is on, as one
	// read in a chroot that is no mount point of its own can, tells none of
	// its aliases: no start, rather than mask the path alone (Codex review
	// of #63, round 4).
	if len(p.Mounts) > 0 {
		for _, path := range slices.Concat(hide, readOnly, resolvedShown) {
			if _, ok := mountAt(p.Mounts, path); !ok {
				return nil, fmt.Errorf("commands run in a sandbox (ADR-0012), and %s: %s, so pocketd cannot tell where else a mount shows it", errNoMount, path)
			}
		}
	}
	s.roots, s.mounts = resolvedShown, p.ReadMounts
	if s.roots == nil {
		s.roots = []string{}
	}
	s.Shown = s.unmasked(withAliases(resolvedShown, p.Mounts))

	// Every folder above a protected path whose parent the user may
	// change could be renamed, taking the protected path with it, under
	// every path a mount shows it at: a bind mount of part of a protected
	// tree below a folder of the user's would otherwise move with that
	// folder, out from under its mask (Codex review of #63, round 1).
	for _, path := range slices.Concat(s.Hide, s.ReadOnly, s.Shown) {
		for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if p.Writable(filepath.Dir(dir)) && !slices.Contains(s.Pin, dir) {
				s.Pin = append(s.Pin, dir)
			}
		}
	}
	// Shallowest first, so a pin never covers a mount below it; by name
	// within a depth, so the command line is the same on every start.
	slices.SortFunc(s.Pin, func(a, b string) int {
		if d := strings.Count(a, string(filepath.Separator)) - strings.Count(b, string(filepath.Separator)); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	// Each shown folder as it is now is the one kept (keepShown).
	if err := s.keepShown(context.Background()); err != nil {
		return nil, err
	}
	for _, named := range p.Shown {
		if named == nil || *named == "" {
			continue
		}
		if dir, err := filepath.Abs(*named); err == nil {
			*named = kept[dir]
		}
	}
	return s, nil
}

// noUserLinks refuses path when a symbolic link on its way, the links it
// leads through included, lives in a folder the user may change: the user
// could point it elsewhere.
func noUserLinks(path string, writable func(string) bool) error {
	_, err := walkPath(path, func(dir string, entry fs.FileInfo) error {
		if entry.Mode()&os.ModeSymlink != 0 && writable(dir) {
			return fmt.Errorf("commands run in a sandbox (ADR-0012), and %s is a symbolic link the service user can change: pocketd cannot protect %s",
				filepath.Join(dir, entry.Name()), path)
		}
		return nil
	})
	return err
}

// linksStayProtected checks every link under dir, since systemd follows
// it: each entry on the way, the links it leads through included, must be
// read-only or pinned in the sandbox, or in a folder only root can change,
// and so must the file it ends at. /dev/null, where systemctl mask points a
// unit, is fine.
func linksStayProtected(dir string, protected []string) error {
	inProtected := func(path string) bool {
		for _, p := range protected {
			if within(path, p) {
				return true
			}
		}
		return false
	}
	holdsProtected := func(path string) bool {
		for _, p := range protected {
			if within(p, path) {
				return true
			}
		}
		return false
	}
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}
		refuse := func(err error) error {
			return fmt.Errorf("commands run in a sandbox (ADR-0012), and %s leads through something the service user can change: %w", path, err)
		}
		target, err := walkPath(path, func(dir string, entry fs.FileInfo) error {
			// An entry inside a protected path is read-only, and one that
			// holds a protected path is pinned: neither can change.
			next := filepath.Join(dir, entry.Name())
			if inProtected(next) || holdsProtected(next) {
				return nil
			}
			uid, perm, err := statOwner(dir)
			if err != nil {
				return err
			}
			if uid != 0 || perm&0o022 != 0 {
				return fmt.Errorf("%s can be changed by a user other than root", dir)
			}
			return nil
		})
		if err != nil {
			return refuse(err)
		}
		if inProtected(target) || target == os.DevNull {
			return nil
		}
		info, err := os.Lstat(target)
		if err != nil {
			return refuse(err)
		}
		if err := rootOwnedEntry(target, info); err != nil {
			return refuse(err)
		}
		return nil
	})
}

// absolute is path made absolute, with its symbolic links resolved.
func absolute(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

// mount is a line of /proc/self/mountinfo: the filesystem's device, the
// folder of that filesystem the mount shows, and where; the mount's id and
// its parent's, and its own options ("ro" for a read-only bind).
type mount struct {
	Device, Root, Point string
	ID, Parent, Options string
}

// parseMountinfo reads /proc/self/mountinfo.
func parseMountinfo(r io.Reader) ([]mount, error) {
	var out []mount
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		// Fields are apart by one ASCII space; a path holds none, escaped,
		// but may hold other white space as it is (Codex review of #63,
		// round 6).
		fields := strings.Split(sc.Text(), " ")
		if len(fields) < 5 {
			continue
		}
		m := mount{ID: fields[0], Parent: fields[1], Device: fields[2], Root: unescapeMount(fields[3]), Point: unescapeMount(fields[4])}
		if len(fields) > 5 {
			m.Options = fields[5]
		}
		out = append(out, m)
	}
	return out, sc.Err()
}

// mountAt is the mount path is on in the mount table mounts, as the
// kernel walks to it: from the root, into the first child mount the walk
// down path crosses, and up each stack of mounts at one point to its top,
// until no child holds path. A mount another covers is never on the way.
// ok is false for a table with no root.
func mountAt(mounts []mount, path string) (mount, bool) {
	listed := map[string]bool{}
	for _, m := range mounts {
		listed[m.ID] = true
	}
	// top is the mount stacked last at m's point: each is a child of the
	// one it covers, at the same point.
	top := func(m mount) mount {
		for range mounts {
			next := -1
			for i, c := range mounts {
				if c.Parent == m.ID && c.ID != m.ID && c.Point == m.Point {
					next = i
				}
			}
			if next < 0 {
				break
			}
			m = mounts[next]
		}
		return m
	}
	var cur mount
	found := false
	for _, m := range mounts {
		if m.Point == "/" && (!listed[m.Parent] || m.Parent == m.ID) {
			cur, found = top(m), true
			break
		}
	}
	if !found {
		return mount{}, false
	}
	for range mounts {
		next := -1
		for i, c := range mounts {
			if c.Parent != cur.ID || c.ID == cur.ID || c.Point == cur.Point || !within(path, c.Point) {
				continue
			}
			// The walk crosses the nearest mount first: a deeper child of
			// the same mount was covered when the nearer one came.
			if next < 0 || len(c.Point) <= len(mounts[next].Point) {
				next = i
			}
		}
		if next < 0 {
			break
		}
		cur = top(mounts[next])
	}
	return cur, true
}

// readOnly says whether a mount's own options hold "ro".
func (m mount) readOnly() bool {
	return slices.Contains(strings.Split(m.Options, ","), "ro")
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space).
func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// withAliases is paths, each followed by every other path a mount exposes
// it at: a bind mount of /var/home at /home shows the same data folder
// twice, and a mask on one path leaves the other.
func withAliases(paths []string, mounts []mount) []string {
	var out []string
	add := func(p string) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	// A filesystem mounted inside a protected tree, such as a runtime
	// folder's own tmpfs under /run/user, is protected as well, and may be
	// mounted elsewhere too.
	roots := slices.Clone(paths)
	for _, path := range paths {
		for _, m := range mounts {
			if m.Point != path && within(m.Point, path) && !slices.Contains(roots, m.Point) {
				roots = append(roots, m.Point)
			}
		}
	}
	// An alias inside a path already masked is covered by that mask.
	addAlias := func(p string) {
		for _, path := range paths {
			if p != path && within(p, path) {
				return
			}
		}
		add(p)
	}
	for _, path := range roots {
		if slices.Contains(paths, path) {
			add(path)
		}
		// The mount the path is on, as the kernel walks to it: a mount
		// with a longer point that another mount covers is not it (Codex
		// review of #63, round 3).
		found, ok := mountAt(mounts, path)
		if !ok {
			continue
		}
		under := &found
		rel, err := filepath.Rel(under.Point, path)
		if err != nil {
			continue
		}
		inFS := filepath.Join(under.Root, rel)
		// A path a mount would show the tree at is an alias only where
		// that mount is the one seen there: one another mount covers shows
		// something else (Codex review of #63, round 4).
		seen := func(alias string, m mount) bool {
			at, ok := mountAt(mounts, alias)
			return ok && at.ID == m.ID
		}
		for _, m := range mounts {
			if m.Device != under.Device {
				continue
			}
			switch {
			case within(inFS, m.Root):
				// The mount shows a folder that holds the path.
				if rest, err := filepath.Rel(m.Root, inFS); err == nil && seen(filepath.Join(m.Point, rest), m) {
					addAlias(filepath.Join(m.Point, rest))
				}
			case within(m.Root, inFS):
				// The mount shows part of the path's tree on its own.
				if seen(m.Point, m) {
					addAlias(m.Point)
				}
			}
		}
	}
	return out
}

// shell is the shell PiG's local environment runs a string with:
// /bin/bash, else bash on the PATH, else sh.
func shell() string {
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		return bash
	}
	return "sh"
}

// systemProgram is name in /usr/bin or /bin, if only root can change it.
func systemProgram(name string) (string, error) {
	var last error
	for _, dir := range []string{"/usr/bin", "/bin"} {
		path, err := trustedProgram(filepath.Join(dir, name))
		if err == nil {
			return path, nil
		}
		last = err
	}
	return "", fmt.Errorf("no %s only root can change: %w", name, last)
}

// bwrapPaths are where a system installs bwrap. The PATH is never searched:
// a folder on it that the service user owns could hold a fake bwrap.
var bwrapPaths = []string{"/usr/bin/bwrap", "/bin/bwrap", "/usr/local/bin/bwrap"}

// trustedBwrap is the first installed bwrap that only root can change,
// along every name that leads to it.
func trustedBwrap() (string, error) {
	var problems []string
	for _, candidate := range bwrapPaths {
		if _, err := os.Lstat(candidate); err != nil {
			continue
		}
		program, err := trustedProgram(candidate)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		return program, nil
	}
	if len(problems) > 0 {
		return "", fmt.Errorf("commands run under bubblewrap (ADR-0012), and no bwrap only root can change is installed: %s", strings.Join(problems, "; "))
	}
	return "", fmt.Errorf("commands run under bubblewrap (ADR-0012), and bwrap is not installed in %s", strings.Join(bwrapPaths, ", "))
}

// trustedProgram follows path to the program it names, the way the kernel
// does, and checks that only root can change each step: every folder it
// passes through, links included, and the program itself.
func trustedProgram(path string) (string, error) {
	resolved, err := walkPath(path, func(dir string, _ fs.FileInfo) error {
		uid, perm, err := statOwner(dir)
		if err != nil {
			return err
		}
		if uid != 0 || perm&0o022 != 0 {
			return fmt.Errorf("%s can be changed by a user other than root", dir)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a program", resolved)
	}
	if err := rootOwnedEntry(resolved, info); err != nil {
		return "", err
	}
	return resolved, nil
}

// Check proves the sandbox before pocketd starts (ADR-0012): it runs bwrap
// with no shell and the commands' own environment, and refuses unless the
// command runs as process 1's child of bwrap, finds every hidden folder
// empty, and finds each read-only path and shown folder (ADR-0035) on a
// read-only mount, in its own mount table, read with root's cat. A fake
// bwrap, or a mount the masks or the read-only binds missed, fails it.
// newSandbox made every shown folder that was missing, so each is proved,
// a host's first start included.
func (s *Sandbox) Check(ctx context.Context) error {
	run := func(argv ...string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		args, err := s.CommandContext(ctx, argv)
		if err != nil {
			return "", err
		}
		//nolint:gosec // G204: the program is a root-owned bwrap, and the arguments are the sandbox's own.
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Env = []string{}
		for name, value := range allowedEnv() {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
		out, err := cmd.Output()
		return string(out), err
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("commands run under bubblewrap (ADR-0012), and the sandbox check failed: "+format, args...)
	}
	// The probes are root's own programs, never ones found on the PATH.
	cat, err := systemProgram("cat")
	if err != nil {
		return fail("%v", err)
	}
	ls, err := systemProgram("ls")
	if err != nil {
		return fail("%v", err)
	}
	if out, err := run(cat, "/proc/1/comm"); err != nil || out != "bwrap\n" {
		return fail("process 1 is %q, not bwrap (%v)", strings.TrimSpace(out), err)
	}
	// A mask that shows pocketd's program for the gate shows it alone.
	gate, bind := s.gate()
	for _, dir := range s.Hide {
		want := ""
		if bind != nil && filepath.Dir(gate) == dir {
			want = gateName + "\n"
		}
		if out, err := run(ls, "-A", dir); err != nil || out != want {
			return fail("%s is not empty in the sandbox (%v)", dir, err)
		}
	}
	// The sandbox's own mount table says, for each read-only path, which
	// mount it is on, and whether that mount is read-only: a permission
	// that refuses a write proves nothing of the files below, and a probe
	// that did not run proves nothing at all (Codex review of #63, round
	// 1). A path a mask hides is an empty tmpfs there, which writes nothing
	// to the host.
	out, err := run(cat, "/proc/self/mountinfo")
	if err != nil {
		return fail("the sandbox's mount table could not be read (%v)", err)
	}
	mounts, err := parseMountinfo(strings.NewReader(out))
	if err != nil {
		return fail("the sandbox's mount table could not be read (%v)", err)
	}
	for _, path := range slices.Concat(s.ReadOnly, s.Shown) {
		if s.Hides(path) {
			continue
		}
		m, ok := mountAt(mounts, path)
		if !ok || !m.readOnly() {
			return fail("%s is on a mount that is not read-only in the sandbox (%q at %q)", path, m.Options, m.Point)
		}
	}
	return nil
}
