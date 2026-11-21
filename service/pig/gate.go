package pig

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// GateHelper is the hidden pocketd command every command of a sandbox
// that shows folders starts through, inside the sandbox: GateHelper
// FOLDER... [placed PLACE...] [masked MASK...] -- COMMAND.... It reads the
// sandbox's own mount table, and runs COMMAND in its place only when
// every path a mount shows a FOLDER or a PLACE, or a part of one, at is
// on a read-only mount there (ADR-0035). So a command admitted before a
// mount showed a shown folder somewhere new, and started after, refuses
// to run, rather than run with binds that do not cover it (Codex review
// of #63, round 6). A PLACE names a shown folder's files by device, so
// one a mask hides is checked too (placesOf, round 8).
const GateHelper = "__sandbox-gate"

// gateExec replaces the gate with the command (execve(2)); tests never
// let it.
var gateExec = syscall.Exec

// gateMounts reads the sandbox's mount table; tests give it one.
var gateMounts = readMountinfo

// gateRefused is the exit status of a command the gate refused.
const gateRefused = 125

// RunGate is GateHelper's work, with its arguments after the command's
// name: it replaces itself with the command, or writes why it refused to
// w and gives its exit status.
func RunGate(args []string, w io.Writer) int {
	split := slices.Index(args, "--")
	if split < 0 || split == len(args)-1 {
		_, _ = fmt.Fprintln(w, "pocketd's sandbox gate: no command to run")
		return 2
	}
	folders, command := args[:split], args[split+1:]
	var places, masks []string
	if i := slices.Index(folders, gateMasked); i >= 0 {
		folders, masks = folders[:i], folders[i+1:]
	}
	if i := slices.Index(folders, gatePlaced); i >= 0 {
		folders, places = folders[:i], folders[i+1:]
	}
	if err := gateCheck(folders, places, masks); err != nil {
		_, _ = fmt.Fprintf(w, "pocketd's sandbox refused the command: %v; restart pocketd\n", err)
		return gateRefused
	}
	program := command[0]
	if !filepath.IsAbs(program) {
		found, err := exec.LookPath(program)
		if err != nil {
			_, _ = fmt.Fprintf(w, "pocketd's sandbox gate: %v\n", err)
			return 127
		}
		program = found
	}
	//nolint:gosec // G204: the command is the sandbox's own, given by pocketd.
	err := gateExec(program, command, os.Environ())
	_, _ = fmt.Fprintf(w, "pocketd's sandbox gate: run %s: %v\n", program, err)
	return 126
}

// gateMasked comes between the gate's folders and the masks inside them:
// the paths the sandbox empties with a tmpfs of its own. A path is
// absolute, so no folder is this word.
const gateMasked = "masked"

// gatePlaced comes between the gate's folders and the places of the
// shown folders' files (placesOf). A place starts with its device's
// number, so no place is this word.
const gatePlaced = "placed"

// gateCheck is nil when, in this process's mount table, each folder is on
// a read-only mount, and so is every other path a mount shows one, or a
// part of one, at, every path a mount shows a place at, and every mount
// seen inside a folder, but the masks.
func gateCheck(folders, places, masks []string) error {
	mounts, err := gateMounts()
	if err != nil {
		return err
	}
	// A place is found by its device, the host's, in every mount of it
	// that holds it or shows part of it: a path seen there shows the
	// place's own files, which a mask over its folder's path does not hide
	// (Codex review of #63, round 8). A path another mount covers, a
	// mask's included, shows something else.
	for _, place := range places {
		i := strings.IndexByte(place, '/')
		if i <= 0 {
			return fmt.Errorf("%q names no place", place)
		}
		device, inFS := place[:i], filepath.Clean(place[i:])
		for _, m := range mounts {
			if m.Device != device {
				continue
			}
			var path string
			switch {
			case within(inFS, m.Root):
				rest, err := filepath.Rel(m.Root, inFS)
				if err != nil {
					continue
				}
				path = filepath.Join(m.Point, rest)
			case within(m.Root, inFS):
				path = m.Point
			default:
				continue
			}
			if at, ok := mountAt(mounts, path); !ok || at.ID != m.ID {
				continue
			}
			if !m.readOnly() {
				return fmt.Errorf("a mount shows a shown folder's files at %s, which is not read-only here (%s)", path, strings.TrimSpace(m.Options))
			}
		}
	}
	for _, path := range withAliases(folders, mounts) {
		m, ok := mountAt(mounts, path)
		if !ok {
			return fmt.Errorf("%s: %w", path, errNoMount)
		}
		if !m.readOnly() {
			return fmt.Errorf("a mount shows a shown folder at %s, which is not read-only here (%s)", path, strings.TrimSpace(m.Options))
		}
	}
	// A mount inside a folder shows its own files there, which no alias
	// names: one seen there must be read-only too, but a mask, an empty
	// tmpfs that shows nothing of the owner's (Codex review of #63, round
	// 7).
	for _, m := range mounts {
		inside := false
		for _, folder := range folders {
			inside = inside || m.Point != folder && within(m.Point, folder)
		}
		if !inside || slices.Contains(masks, m.Point) {
			continue
		}
		if at, ok := mountAt(mounts, m.Point); !ok || at.ID != m.ID {
			continue // covered: not seen
		}
		if !m.readOnly() {
			return fmt.Errorf("a mount shows its files at %s, inside a shown folder, and is not read-only here (%s)", m.Point, strings.TrimSpace(m.Options))
		}
	}
	return nil
}
